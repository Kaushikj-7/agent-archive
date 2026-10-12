# Setup and CLI experience implementation plan

Status: implemented on the default branch. Reconciled 2026-10-11 at `db2ccd3e`. Shared prompts, setup selection/review, and guided commands merged in #354, #357 and #358. Platform acceptance and release sign-off remain separate gates; the original pending-review wording below is historical.

## Original design and execution record

Status: Accepted design; shared rendering, setup and remaining guided commands
are implemented in dependent changes. Final integration review, required gates
and platform acceptance remain pending. See the
[renderer contract](../../contributing/prompt-rendering.md).

Make guided commands clearly distinguish completed answers, explanatory text,
and the current question. Initial onboarding and later interactive setup use
one project selector, with **All N found projects** as the visible default.
Shorten the final review while retaining the facts needed to approve capture,
and apply the same prompt vocabulary to pairing, handoff, backfill, and removal.

## Scope and accepted behavior

| Area | Required behavior |
| --- | --- |
| Completed answers | Collapse an answered menu into a short receipt where the terminal supports safe redraw. Subdue completed receipts when color is available. |
| Active question | Use `?` before the question and `›` at the input line. Keep structural markers when color is disabled; use `>` in a dumb terminal. |
| Spacing | One blank line between logical blocks. Keep a question, its explanation, choices, and answer cursor together. |
| Projects | Offer All N found projects as choice 1 and the default; choice 2 opens specific selection. All includes the entire candidate set, regardless of pagination. |
| Consistency | Initial setup, Apps and projects reconfiguration, All settings, and Projects in the review edit menu call the same selector. Resumed unfinished project selection uses it too. |
| Final review | Show essential settings, meaningful changes, checks, and capture/privacy implications before the confirmation. Put remaining settings behind Details. |
| Completion | Print a clear setup completion boundary, app-specific next steps, then a separate optional history import. |
| Other commands | Reuse spacing, markers, grouping, wrapping, and receipts. Preserve existing output formats, command behavior, and confirmation requirements. |

Onboarding is the first-run path of `agent-archive setup`; there should be no
second onboarding wizard. Installation continues to install the executable
without automatically starting setup or changing hooks.

The numbered choices and shortcuts in the examples are the new interactive
presentation. Preserve compatible named answers and existing command flags.
Update scripted interactive examples and fixtures whose numeric positions
change. Do not treat positional wizard answers as a stable automation API;
the explicit noninteractive flags remain the supported route for automation.

## Shared project selection

### The visible default

```text
Step 1 of 3
Choose what to capture

✓ Apps
  Codex, Claude Code, Cursor

? Which projects?

  Found 3 projects:
    ~/src/web-app · 24 sessions
    ~/src/api     · 12 sessions
    ~/src/docs    ·  3 sessions

  1) All 3 projects (default)
  2) Choose specific projects

› Choose [1]:
```

All means every valid project in the selector's complete candidate set. It
does not mean every directory on the computer, every archived remote project,
or every project created in the future. Codex's Included projects only versus
All current and future projects setting stays a separate explicit question.
If Codex-only future-project scope makes project selection unnecessary,
explain that scope and offer the existing project-exceptions editor rather
than forcing a meaningless list selection.

The current repository belongs in the candidate list, first and labeled
`this folder`, but must not bypass the All versus Specific question.

### Specific selection

```text
? Choose specific projects

  1) [✓] ~/src/web-app · 24 sessions
  2) [✓] ~/src/api     · 12 sessions
  3) [✓] ~/src/docs    ·  3 sessions

Enter numbers to toggle.

  [a] Select all
  [p] Add a project path
  [Enter] Confirm selection

› Projects:
```

Fresh setup starts Specific with all candidates checked, matching the
agreed example. Editing an existing or saved draft selection starts Specific
with its actual choices checked. Returning from Add a path preserves the
selection, page, and existing activation times. Repeated numbers in a single
answer toggle each project once, and overlapping ranges behave the same way.

All is the default whenever the user enters project selection. Continuing a
draft whose project step is complete keeps its saved choices; it does not
choose All again. Storage-only and retention-only reconfiguration must not
change projects. Selecting All while editing projects includes previously
unchecked candidate roots too; display the number being added or re-enabled
and show those changes before Save. Exclusion rules outside the candidate
set remain intact. Specific selection preserves imported-project exclusions
and nearest-explicit-rule behavior when a project is left out.

### Candidate collection and large lists

Extract selection from the rendering code in `projectPicker.offer`. Build a
canonical, deduplicated candidate model from the current repository, saved
project roots, and discovered eligible roots before rendering. Reuse existing
path validation, symlink handling, nested-project folding, backfill ownership,
and activation rules. Keep explicit nested exceptions as rules even when
session counts roll up under a displayed parent.

`maxKnownProjects = 12` currently limits the roots offered by the picker.
Make it a display page-size limit instead. All must operate on the complete
validated model, never the current page. Display `Showing 1–12 of 37` and
provide Next/Previous actions; keep project numbers stable across pages.
Select all works across every page. Manually added roots join the same model.

Keep discovery bounded and report its coverage. `knownProjectsOnce` currently
turns a failure into an empty list; return completeness/error information so
No projects found is distinguishable from Could not finish looking.
Reuse the existing `KnownProjectsResult` shape and bounded discovery machinery
where appropriate, but account for a concrete gap: `KnownProjectsBounded`
currently yields roots without the session counts and last-used values that
`KnownProjects` supplies. Preserve or add bounded aggregation of those fields
before adopting that path for the selector. Do not display missing counts as
zero or imply that partial counts are exact.

When discovery is partial, say `Found N projects; search incomplete`, explain
the reason briefly, and label the default `All N found projects`. Offer Retry
and Add a path. When no valid candidates exist, open Add a path instead of
offering All 0 projects. Validation failures stay with that field and retain
prior choices. Selected directories that disappear must be reported before
selection can be confirmed; never silently reduce All to an unexplained subset.

Cache discovery for the applicable apps, source roots, and project rules;
invalidate on relevant edits or explicit Retry. Rendering, paging, and
validation retries must not repeatedly scan session stores.

## Prompt rendering architecture

Extend the existing Go terminal helpers rather than replacing the CLI with
a new framework. Keep business decisions in command code and presentation in
the shared prompt renderer.

Introduce a small prompt model in `internal/cli/prompt.go` or a focused new
`prompt_render.go`: question, helper lines, numbered primary choices,
unnumbered secondary actions, default, and a completion receipt. Keep option
keys separate from visible labels. Route `yesNo`, `menu`, `actions`, ordinary
text input, and command-specific confirmation prompts through this model.
Receipts come from resolved values, so Enter produces `✓ Profile work`, not
a blank or a receipt containing only the numeric answer.

Separate terminal capabilities from styling: color, input/output terminal
presence, safe redraw, dimensions, and whether input is secret. `NO_COLOR`
turns off color and emphasis, not spacing, markers, or otherwise-supported
interaction. A dumb terminal uses static ASCII structure. Redirected streams
get static output without cursor movement or synthesized answer echo.
Structured JSON/JSON Lines outputs remain free of decorative prompt text.

### Owning the active prompt region

On a normal terminal, render only the current question and its choices as a
managed block at the bottom of the screen. After a valid answer, replace that
block with its receipt, then append a blank line and the next block. Preserve
completed receipts in ordinary scrollback. Setup does not need a full-screen
alternate buffer; the pairing code display keeps its existing dedicated
alternate-screen lifecycle.

Track rendered rows using `visibleWidth` and `displayLines`, including wrapped
helpers and echoed nonsecret input. Restrict erasure to the owned region.
Do not redraw over external subprocess output, an AWS credential helper, or
a spinner. Stop or suspend prompt redraw while those components own the
terminal; re-establish a fresh region afterward.

Retain buffered line input and its typed-ahead answers. Do not introduce a
second stdin reader merely to implement menus. Detect a width change before
any region replacement; if its old rows can no longer be identified safely,
invalidate that region and append a fresh receipt/block rather than erasing
unrelated terminal content. Use the same fallback if the active block or
echoed answer has scrolled beyond the visible viewport. Handle suspend/resume and interrupted reads the
same way. The static fallback keeps its expanded history but uses the same
question grouping and answer spacing.

Centralize blank-line emission in the renderer and remove leading/trailing
newline workarounds from `step`, `heading`, and callers. Replace the
setup-only `spaceAfterAnswer` policy with an explicitly shared guided prompt
policy; avoid changing every low-level `line` call blindly, since browsers
and special secret-input flows also use it.

Secret input may produce `✓ Credential received` but must never put its
value, length, clipboard contents, or pairing code in a receipt or retained
render state. EOF remains an error, not acceptance of a default. Keep terminal
restoration and setup's existing signal exit statuses.

## Storage and error frames

Use the same renderer in provider selection, Create versus Existing,
AWS profile/bucket prompts, guided R2/S3 creation, credential retries, and
storage failures. Headings precede helper text; numbered choices contain
the primary decisions; navigation and diagnostics appear separately.

```text
Step 2 of 3 · Connect storage

✓ Provider   Amazon S3
✓ Connection Existing bucket
✓ Profile    work

? Which bucket should store sessions?

  1) photos
  2) team-archive (default)

Enter a number or bucket name.
  [b] Back to storage options

› Bucket [2]:
```

Add Back only at points where command state can safely return. Returning
from provider creation must retain the existing staged-resource accounting
and cleanup behavior; it must not accidentally create another bucket or key.

```text
✗ Cannot sign in with profile work.
  Your SSO session has expired.

Fix: run in another terminal:
  aws sso login --profile work

? What next?

  1) Retry connection (default)
  2) Choose another profile
  3) Change storage

  [d] Diagnostic details
  [q] Stop; keep your draft

› Choose [1]:
```

Use the existing diagnosis to choose the fix; SSO login is appropriate only
for an actual SSO failure. Details reveals the existing diagnostic payload
without requiring a restart and returns to the same retry decision.

## Final review and completion

Build a review model once from the draft, existing configuration, and current
checks. Render a compact summary and an expanded details view from that same
model. Do not maintain two independent interpretations of configuration.

Compact review includes apps, project count or short list, Codex scope,
storage destination including prefix, retention, and skill evidence scope.
Include nondefault capture restrictions, project exclusions, and changed
settings even when they would normally live in Details. Show old/new values
for meaningful changes. Display destination once rather than repeating
Destination and Storage rows.

Compute label alignment from visible label widths; at narrow widths put the
value on an indented following line. Wrap explanations with hanging
indentation. Summarize many projects by count with their complete paths in
Details. Keep checks distinct; never say the bucket is private when inspection
is unknown, or that hooks are approved when only their files are valid.

```text
Step 3 of 3 · Review and start

  Apps       Codex, Claude Code, Cursor
  Projects   3 found projects
  Codex      Included projects only
  Storage    s3://team-archive/agent-archive/
  Keep for   90 days
  Skills     Metadata, including user folders

✓ Storage connected · bucket private
! History import is separate.
! Sensitive text may remain after filtering.

? Start archiving?

  1) Start archiving (default)
  2) Edit a setting

  [d] Full settings and privacy
  [m] Name this machine
  [q] Cancel; keep draft

› Choose [1]:
```

Aim for the representative summary, checks, implications, and answer cursor
to fit an 80-column, 24-row terminal. At 60 columns, retain all essential
warnings and allow additional rows rather than dropping information to hit
that target. Blocking checks remove Start/Save from the available actions
and keep Check again as the primary decision, as today.

Details includes app versions, source roots, exact project paths and
exceptions, discovery/copy behavior, hook approval status, storage checks,
credential ownership where applicable, skill scope, and the full privacy
documentation link. Page longer detail views using the existing pager
lifecycle and return to the same review with the draft untouched.

After the existing setup transaction succeeds, print completion, summarize
installed skills, and give app-specific next steps. Then render the optional
import as a new task. If setup succeeds but import fails or is cancelled,
state that setup is complete and provide the existing retry command.

```text
✓ Setup complete · Automatic capture is on

Start a new session in an included project.
  Codex: new task; discovery needs no approval.
  Claude Code: new session or /clear.
  Cursor: new Agent chat.

Optional Codex hooks: approve with /hooks.
Check capture: agent-archive status

Optional · Import past sessions

Found 36 sessions in 3 selected projects.
90-day retention applies.

? Import these sessions?
  1) Import 36 sessions (default)
  2) Skip for now

› Choose [1]:
```

Retain the current setup import default; standalone backfill retains its
own explicit confirmation/default. Move long machine-transfer recipes out
of the default completion transcript into an optional next-step details
action. Keep a short discoverable hint to machine pairing.

## Implementation sequence

Each change is independently reviewable, but the project is complete only
when all steps below are delivered. Implement sequentially so the renderer
and selector are shared before individual flows are migrated.

| Step | Implementation | Completion evidence |
| --- | --- | --- |
| 1 Shared prompt rendering | Extend `prompt.go`, `ui.go`, and focused renderer helpers. Add capabilities, grouped prompt models, safe receipts, wrapping, and centralized spacing. Integrate with one setup menu and its secret-input path before broader migration. | Plain/color snapshots; terminal tests for answer collapse, EOF, hidden credentials, resize invalidation, signals, and typed-ahead input. |
| 2 Shared project selector | Add a focused `setup_projects.go` for candidate modeling and selection. Refactor `chooseCapture`, `offerFirstCapture`, `promptProjects`, `addProjects`, and the Projects edit branch to use it. Separate complete candidates from display pages. Preserve discovery metadata and selection ownership rules. | Inside/outside-repository setup, custom selection, more than 12 roots, incomplete discovery, exclusions, imported roots, disappearing paths, and saved-draft tests. |
| 3 Setup storage and review | Migrate `setup_storage.go`, `setup_aws.go`, `setup_r2_create.go`, `setup_s3_create.go`, storage diagnostics/checks, and `setup_review.go`. Add compact/expanded review from one model. Keep `setup_transaction.go` as the commit authority. | Existing provider retry, staged-resource cleanup, draft resume, blocking-review, transaction rollback, and reconfiguration checks pass with updated screens. |
| 4 Completion and import | Update setup's finish path and `offerSetupImport` in `backfill.go`. Separate completion, app steps, import, and machine-pairing guidance. | Successful setup with no history, import accepted/declined/failed, hook-only versus discovery modes, and partial app detection have complete transcripts. |
| 5 Other guided commands | Migrate pairing source/receiver and management-token prompts, handoff destination/file/checkout prompts, backfill/edit/undo, uninstall, and purge confirmations. Group recovery consequences. Add headings/alignment to human machine listings. | Full transcripts demonstrate shared boundaries and defaults; secret pairing display, browser/pager handoffs, destructive confirmations, and structured-output contracts still pass. |
| 6 Documentation and final acceptance | Update `docs/getting-started/setup.md`, affected guides/help, CLI reference where necessary, and `CHANGELOG.md`. Review every changed golden. Run repository gates and disposable terminal smoke flows on macOS and Linux. | The acceptance matrix below passes, remaining limitations are recorded, and this proposal becomes an implementation record after shipment. |

Leave the already-strong list/show/stats browsers and root help organization
in place. Any changes there should be integration fixes needed for shared
prompts, paging, or stream ownership. Keep sync, pause/resume, feedback, and
evaluation output concise and preserve their output contracts.

## Acceptance and verification

### Behavioral acceptance

- Fresh onboarding inside a repository and outside it displays the same
  All versus Specific project question. Enter chooses every valid found
  candidate, including a candidate on a later display page.
- Apps and projects reconfiguration, All settings, and the review Projects
  editor use that exact component and choice vocabulary.
- Resuming a completed project step retains its selections. Reentering
  selection explicitly offers All as default. Specific restores saved choices.
- Imported-project exclusions and existing activation times survive the
  same selection changes as today. All explicitly reports re-enabled roots;
  final review shows expanded capture scope before saving.
- No projects, partial discovery, duplicate/symlink roots, nested rules,
  more than 12 projects, manual paths, overlapping ranges, and disappearing
  directories have explicit tested outcomes.
- Only the current prompt remains expanded on supported terminals. Past
  receipts persist in scrollback and never contain credentials or pairing codes.
- Every guided command separates its plan/result, explanation, and active
  decision. Secondary actions use the same bracketed shortcut convention.
- Details returns to the same decision without saving, rerunning discovery,
  reissuing credentials, or losing buffered input.
- Blocked setup cannot start through a hidden alias. EOF and interrupts
  cannot silently accept defaults or commit an unconfirmed draft.
- Completion and optional import are separate, and completion wording
  reflects the actual commit and capture mode.

### Presentation matrix

Use synthetic fixtures and injected `Env`, following the repository's
[testing guide](../../contributing/testing.md). Record normal color and
`NO_COLOR` terminals at 80×24, 100×30, and 60×20; include long paths, wide
characters, and many projects. Add a narrow 36-column static snapshot for
readability in phone-sized examples. Also cover dumb terminals, redirected
input/output, separate stdout/stderr, and existing noninteractive controls.

Extend [screen fixtures](../../../internal/cli/screens_test.go) beyond setup
and status to full guided pairing, handoff, backfill, uninstall, and purge
transcripts. Give synthetic outputs explicit color, redraw, width, and
height capabilities; color alone currently cannot model the proposed UI.
Verify semantic consistency between plain and color frames without freezing
obsolete spacing or old numeric menu positions.

Extend the existing PTY harness with a synthetic setup child. Assert the
terminal's visible cells after menu completion and retries, not just the
presence of ANSI sequences. Cover long echoed answers, hidden input,
typed-ahead multiple answers, resize during a prompt, suspend/resume,
subprocess-owned input, and leaving a pager. Verify terminal restoration
after normal exit, EOF, signals, and error paths. Use disposable homes,
stubbed schedulers/providers, and in-memory stores; never run setup against
the developer's real installation.

Run focused CLI and backfill checks while implementing, then the required
test, vet, lint, documentation, and cross-platform gates from the testing
guide. Review updated golden diffs for content as well as spacing: accepting
a golden update is not evidence that the displayed default or consent is
correct. No schema, filter, or parser-version bump is expected for this UI
work; investigate any implementation that changes archived content.

## Delivery checklist

- [x] Shared prompt renderer and terminal capability handling.
- [x] All found projects default and one selector for every setup entry point.
- [x] Complete candidate set, paginated display, and honest discovery coverage.
- [x] Consistent storage menus, retries, diagnostics, and navigation.
- [x] Compact final review, complete details, and preserved blocked actions.
- [x] Setup completion separated from optional import and pairing guidance.
- [ ] Pairing, handoff, backfill, uninstall, purge, recovery, and machine listing migration.
- [ ] Full transcript goldens, PTY checks, documentation, and platform acceptance.

The setup portion is implemented with focused CLI/backfill regression checks,
setup transcript goldens, and selector PTY coverage on macOS. Independent P2
review and hosted platform gates remain pending. The remaining guided commands
are implemented in a dependent P3 change with complete injected command
transcripts, plain/color width matrices and terminal ownership checks. Their
delivery checkboxes remain pending final prerequisite integration, independent
review, required gates and cross-platform acceptance.
